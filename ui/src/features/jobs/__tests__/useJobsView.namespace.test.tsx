import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import type { Job } from "@/lib/api";
import type { CaesiumEvent } from "@/lib/events";
import { resetNamespaceViewForTests, setStoredNamespace } from "@/features/auth/namespaceView";
import { boundHeldActivity, useJobsView } from "../useJobsView";

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

it("does not let held events of other namespaces displace a selected job's through failed refreshes", async () => {
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
  let sequence = 0;
  const fire = async (jobIds: string[], namespace?: string) => {
    const before = getJobs.mock.calls.filter(([ns]) => ns === "marketing").length;
    act(() => {
      for (const job_id of jobIds) {
        sequence += 1;
        const run_id = `run-${sequence}`;
        const payload = namespace ? { id: run_id, job_id, namespace, status: "running" } : undefined;
        handlers.get("run_started")!({ sequence, type: "run_started", job_id, run_id, timestamp: new Date().toISOString(), payload });
      }
    });
    await waitFor(() => expect(getJobs.mock.calls.filter(([ns]) => ns === "marketing").length).toBeGreaterThan(before));
    await new Promise(resolve => setTimeout(resolve, 50));
  };
  await fire(["n"]); // run-1, held for the selected namespace
  // Newer events from elsewhere, past the per-job and the total hold limits.
  await fire(Array.from({ length: 150 }, (_, i) => `foreign-${i}`), "finance");
  await fire(["foreign-150"], "finance"); // a second failed attempt over the whole held queue
  expect(result.current.activity).toEqual([]);
  phase = "recovered";
  await fire(["n"]);
  await waitFor(() => expect(result.current.activity.map(entry => entry.runId).sort()).toEqual(["run-1", `run-${sequence}`]));
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

it("bounds held events per job and in total, dropping other namespaces' first", () => {
  const held = (jobId: string, n: number, namespace?: string) => ({ jobId, key: `${jobId}:${n}`, namespace });
  const keys = (entries: Array<{ key: string }>) => entries.map(entry => entry.key);

  const oneJob = Array.from({ length: 25 }, (_, i) => held("a", i, "marketing"));
  expect(keys(boundHeldActivity(oneJob, "marketing"))).toEqual(keys(oneJob.slice(5)));

  const selected = held("n", 0, "marketing");
  const untagged = held("u", 0);
  const foreign = Array.from({ length: 150 }, (_, i) => held(`f${i}`, 0, "finance"));
  const kept = boundHeldActivity([selected, untagged, ...foreign], "marketing");
  expect(kept).toHaveLength(100);
  expect(keys(kept)).toEqual(keys([selected, untagged, ...foreign.slice(52)])); // oldest foreign go first

  const unknown = Array.from({ length: 120 }, (_, i) => held(`u${i}`, 0));
  expect(keys(boundHeldActivity(unknown, "marketing"))).toEqual(keys(unknown.slice(20))); // then the oldest
});
