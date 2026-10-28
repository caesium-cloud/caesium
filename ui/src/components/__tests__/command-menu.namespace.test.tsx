import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api, type Job } from "@/lib/api";
import { resetNamespaceViewForTests, setStoredNamespace } from "@/features/auth/namespaceView";
import { CommandMenu } from "../command-menu";

vi.mock("@tanstack/react-router", () => ({ useNavigate: () => vi.fn() }));

const job = (id: string, alias: string, namespace: string) => ({ id, alias, namespace, trigger_id: "t", labels: {}, annotations: {}, paused: false }) as Job;

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Element.prototype.scrollIntoView = vi.fn();
  resetNamespaceViewForTests();
  window.localStorage.clear();
  vi.spyOn(api, "getTriggers").mockResolvedValue([]);
  vi.spyOn(api, "getAtoms").mockResolvedValue([]);
  vi.spyOn(api, "getJobs").mockImplementation(async (namespace?: string) =>
    [job("m", "mkt-job", "marketing"), job("f", "fin-job", "finance")].filter((j) => !namespace || j.namespace === namespace),
  );
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  resetNamespaceViewForTests();
});

it("lists only the selected namespace's jobs and follows a change of selection", async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><CommandMenu /></QueryClientProvider>);
  fireEvent.keyDown(document, { key: "k", ctrlKey: true });
  await screen.findByText("mkt-job");
  expect(screen.getByText("fin-job")).toBeInTheDocument();

  setStoredNamespace("marketing");
  await waitFor(() => expect(screen.queryByText("fin-job")).toBeNull());
  expect(await screen.findByText("mkt-job")).toBeInTheDocument();
  expect(screen.queryByText("fin-job")).toBeNull();
  expect(api.getJobs).toHaveBeenCalledWith("marketing");
});
