import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import type { Job, JobRun } from "@/lib/api";
import type { CaesiumEvent } from "@/lib/events";
import { useJobsView } from "../useJobsView";

const { handlers, getJobs } = vi.hoisted(() => ({ handlers: new Map<string, (e: CaesiumEvent) => void>(), getJobs: vi.fn() }));
vi.mock("@/lib/events", () => ({ events: {
  isHealthy: () => true, subscribeConnection: vi.fn(), unsubscribeConnection: vi.fn(),
  subscribe: (type: string, callback: (e: CaesiumEvent) => void) => handlers.set(type, callback),
  unsubscribe: (type: string) => handlers.delete(type),
} }));
vi.mock("@/lib/api", () => ({ api: { getJobs } }));
afterEach(() => { handlers.clear(); vi.useRealTimers(); });

it("refreshes history during sustained events and releases pending activity for new jobs", async () => {
  vi.useFakeTimers();
  const job = { id: "known", alias: "known-job", paused: false } as Job;
  const discovered = { id: "new", alias: "new-job", paused: false } as Job;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(["jobs"], [job]);
  getJobs.mockResolvedValue([job, discovered]);
  const invalidations = vi.spyOn(client, "invalidateQueries");
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  const { result, unmount } = renderHook(useJobsView, { wrapper });
  // Every event is closer than the old trailing-edge debounce's 250ms wait.
  for (let i = 0; i < 20; i++) {
    await act(async () => {
      handlers.get("run_started")!({ type: "run_started", job_id: "new", run_id: `run-${i}`, timestamp: new Date().toISOString() });
      await vi.advanceTimersByTimeAsync(100);
    });
  }
  expect(invalidations.mock.calls.filter(([filters]) => filters?.queryKey?.[0] === "jobs").length).toBeGreaterThanOrEqual(6);
  expect(getJobs).toHaveBeenCalled();
  expect(result.current.activity.some(entry => entry.jobAlias === "new-job")).toBe(true);
  unmount();
  const before = invalidations.mock.calls.length;
  await vi.advanceTimersByTimeAsync(1000);
  expect(invalidations).toHaveBeenCalledTimes(before);
  client.clear();
});

it("preserves latest execution identity in both list and job caches", () => {
  const latest = { id: "newer", job_id: "job", status: "running", started_at: "2026-10-02T10:00:00Z" } as JobRun;
  const job = { id: "job", alias: "job", latest_run: latest } as Job;
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } });
  client.setQueryData(["jobs"], [job]); client.setQueryData(["job", "job"], job);
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  const { unmount } = renderHook(useJobsView, { wrapper });
  act(() => handlers.get("run_completed")!({ type: "run_completed", timestamp: "2026-10-02T10:01:00Z", payload: { ...latest, id: "older", status: "succeeded", started_at: "2026-10-02T09:00:00Z" } }));
  expect(client.getQueryData<Job[]>(["jobs"])![0].latest_run?.id).toBe("newer");
  expect(client.getQueryData<Job>(["job", "job"])!.latest_run?.id).toBe("newer");
  unmount(); client.clear();
});
