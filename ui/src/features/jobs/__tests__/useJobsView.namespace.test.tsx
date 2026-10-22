import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import type { Job } from "@/lib/api";
import type { CaesiumEvent } from "@/lib/events";
import { resetNamespaceViewForTests, setStoredNamespace } from "@/features/auth/namespaceView";
import { useJobsView } from "../useJobsView";

const { handlers, getJobs } = vi.hoisted(() => ({ handlers: new Map<string, (e: CaesiumEvent) => void>(), getJobs: vi.fn() }));
vi.mock("@/lib/events", () => ({ events: {
  isHealthy: () => true, subscribeConnection: vi.fn(), unsubscribeConnection: vi.fn(),
  subscribe: (type: string, callback: (e: CaesiumEvent) => void) => handlers.set(type, callback),
  unsubscribe: (type: string) => handlers.delete(type),
} }));
vi.mock("@/lib/api", () => ({ api: { getJobs } }));
afterEach(() => { handlers.clear(); resetNamespaceViewForTests(); window.localStorage.clear(); });

it("drops activity of jobs outside a namespace selected after the events arrived", () => {
  const marketing = { id: "m", alias: "mkt-job", namespace: "marketing", paused: false } as Job;
  const finance = { id: "f", alias: "fin-job", namespace: "finance", paused: false } as Job;
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  client.setQueryData(["jobs"], [marketing, finance]);
  client.setQueryData(["jobs", "namespace", "marketing"], [marketing]);
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  resetNamespaceViewForTests();
  const { result, unmount } = renderHook(useJobsView, { wrapper });
  act(() => {
    for (const [sequence, job_id] of [[1, "f"], [2, "m"]] as const) {
      handlers.get("run_started")!({ sequence, type: "run_started", job_id, run_id: `run-${job_id}`, timestamp: new Date().toISOString() });
    }
  });
  expect(result.current.activity.map(entry => entry.jobAlias).sort()).toEqual(["fin-job", "mkt-job"]);
  act(() => setStoredNamespace("marketing"));
  expect(result.current.activity.map(entry => entry.jobAlias)).toEqual(["mkt-job"]);
  expect(result.current.rows.map(row => row.alias)).toEqual(["mkt-job"]);
  unmount(); client.clear();
});

it("holds an event for a job missing from the narrowed list until a refetch says whether it belongs", async () => {
  const existing = { id: "m", alias: "mkt-job", namespace: "marketing", paused: false } as Job;
  const created = { id: "n", alias: "new-mkt-job", namespace: "marketing", paused: false } as Job;
  const other = { id: "f", alias: "fin-job", namespace: "finance", paused: false } as Job;
  let scopedLoads = 0;
  getJobs.mockReset();
  getJobs.mockImplementation(async (namespace?: string) => {
    if (!namespace) return [existing, other, created]; // the switcher's options (two namespaces)
    scopedLoads += 1;
    return scopedLoads === 1 ? [existing] : [existing, created]; // the job appears after the first load
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  resetNamespaceViewForTests();
  setStoredNamespace("marketing");
  const { result, unmount } = renderHook(useJobsView, { wrapper });
  await waitFor(() => expect(result.current.rows.map(row => row.alias)).toEqual(["mkt-job"]));
  act(() => {
    for (const [sequence, job_id] of [[1, "n"], [2, "foreign"]] as const) {
      handlers.get("run_started")!({ sequence, type: "run_started", job_id, run_id: `run-${job_id}`, timestamp: new Date().toISOString() });
    }
  });
  await waitFor(() => expect(result.current.activity.map(entry => entry.jobAlias)).toEqual(["new-mkt-job"]));
  // The foreign job never joins the feed, however long the refetch settles.
  await new Promise(resolve => setTimeout(resolve, 50));
  expect(result.current.activity.map(entry => entry.jobAlias)).toEqual(["new-mkt-job"]);
  unmount(); client.clear();
});

it("keeps a held event through a failed refresh and applies it once a refresh succeeds", async () => {
  const existing = { id: "m", alias: "mkt-job", namespace: "marketing", paused: false } as Job;
  const created = { id: "n", alias: "new-mkt-job", namespace: "marketing", paused: false } as Job;
  const other = { id: "f", alias: "fin-job", namespace: "finance", paused: false } as Job;
  let phase: "initial" | "failing" | "recovered" = "initial";
  getJobs.mockReset();
  getJobs.mockImplementation(async (namespace?: string) => {
    if (!namespace) return [existing, other, created];
    if (phase === "failing") throw new Error("list unavailable");
    return phase === "initial" ? [existing] : [existing, created];
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  resetNamespaceViewForTests();
  setStoredNamespace("marketing");
  const { result, unmount } = renderHook(useJobsView, { wrapper });
  await waitFor(() => expect(result.current.rows.map(row => row.alias)).toEqual(["mkt-job"]));
  phase = "failing";
  const fire = (sequence: number) => act(() => {
    handlers.get("run_started")!({ sequence, type: "run_started", job_id: "n", run_id: `run-${sequence}`, timestamp: new Date().toISOString() });
  });
  fire(1);
  await waitFor(() => expect(getJobs.mock.calls.filter(([ns]) => ns === "marketing").length).toBeGreaterThan(1));
  await new Promise(resolve => setTimeout(resolve, 50));
  expect(result.current.activity).toEqual([]); // a failed refresh decides nothing
  phase = "recovered";
  fire(2); // the next event retries the held batch
  await waitFor(() => expect(result.current.activity.map(entry => entry.runId).sort()).toEqual(["run-1", "run-2"]));
  unmount(); client.clear();
});

it("does not let the in-flight initial list answer for an event that arrived after it started", async () => {
  const existing = { id: "m", alias: "mkt-job", namespace: "marketing", paused: false } as Job;
  const created = { id: "n", alias: "new-mkt-job", namespace: "marketing", paused: false } as Job;
  const other = { id: "f", alias: "fin-job", namespace: "finance", paused: false } as Job;
  let scopedLoads = 0;
  let releaseStale: (jobs: Job[]) => void = () => {};
  getJobs.mockReset();
  getJobs.mockImplementation((namespace?: string) => {
    if (!namespace) return Promise.resolve([existing, other, created]);
    scopedLoads += 1;
    // The first scoped load stays unresolved and, when it finally answers, predates the new job.
    if (scopedLoads === 1) return new Promise<Job[]>(resolve => { releaseStale = resolve; });
    return Promise.resolve([existing, created]);
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  resetNamespaceViewForTests();
  setStoredNamespace("marketing");
  const { result, unmount } = renderHook(useJobsView, { wrapper });
  await waitFor(() => expect(scopedLoads).toBe(1));
  act(() => {
    handlers.get("run_started")!({ sequence: 1, type: "run_started", job_id: "n", run_id: "run-n", timestamp: new Date().toISOString() });
  });
  await waitFor(() => expect(result.current.activity.map(entry => entry.jobAlias)).toEqual(["new-mkt-job"]));
  await act(async () => { releaseStale([existing]); });
  expect(result.current.activity.map(entry => entry.jobAlias)).toEqual(["new-mkt-job"]);
  unmount(); client.clear();
});
