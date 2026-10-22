import { act, renderHook } from "@testing-library/react";
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
