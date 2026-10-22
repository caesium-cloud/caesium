import { act, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api, type Job } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { SessionQueryProvider } from "../SessionQueryProvider";
import { NamespaceSwitcher } from "../NamespaceSwitcher";
import { resetNamespaceViewForTests } from "../namespaceView";

async function login(key: string, whoami: Record<string, unknown>) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => whoami }));
  expect(await apiKeyLogin(key)).toBe("success");
}

const clusterViewer = { kind: "api_key", subject: "cluster-key", role: "viewer", cluster_role: "viewer", grants: { "*": "viewer" }, namespaces: ["*"], is_scoped: false };
const scopedKey = { kind: "api_key", subject: "scoped-key", role: "viewer", cluster_role: "viewer", grants: { "*": "viewer" }, namespaces: ["*"], is_scoped: true };
const create = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  window.localStorage.clear();
  resetNamespaceViewForTests();
});
afterEach(() => vi.unstubAllGlobals());

// Real query and mutation consumers, as the Console's pages are: an observer
// keeps the client it was created with, so only a remount fences a session.
function Consumer({ fetchJob, finishPause }: { fetchJob: () => Promise<string>; finishPause: () => Promise<string> }) {
  const client = useQueryClient();
  const job = useQuery({ queryKey: ["job", "j"], queryFn: fetchJob });
  const pause = useMutation({
    mutationFn: finishPause,
    onSuccess: (value) => client.setQueryData(["job", "j"], value),
  });
  return (
    <div>
      <span data-testid="job">{job.data ?? "none"}</span>
      <button onClick={() => pause.mutate()}>pause</button>
    </div>
  );
}

it("a signed-in identity change remounts consumers on a fresh client: no foreign data, no late write", async () => {
  let release!: (value: string) => void;
  const lateResponse = new Promise<string>((resolve) => (release = resolve));
  const owner = { current: "A" };
  const fetchJob = vi.fn(async () => `private-job-of-${owner.current}`);
  const tree = (
    <SessionQueryProvider create={create}>
      <Consumer fetchJob={fetchJob} finishPause={() => lateResponse} />
    </SessionQueryProvider>
  );

  await login("csk_live_a", clusterViewer);
  render(tree);
  await waitFor(() => expect(screen.getByTestId("job")).toHaveTextContent("private-job-of-A"));
  act(() => screen.getByText("pause").click()); // A's mutation is now in flight

  // B signs in before the response arrives. B's own fetch never resolves, so
  // anything B's page shows came from somewhere else.
  owner.current = "B";
  fetchJob.mockImplementation(() => new Promise<string>(() => undefined));
  await login("csk_live_b", { ...clusterViewer, subject: "someone-else" });
  await waitFor(() => expect(screen.getByTestId("job")).toHaveTextContent("none"));

  await act(async () => {
    release("late-write-from-A");
    await lateResponse;
  });
  expect(screen.getByTestId("job")).toHaveTextContent("none");
});

it("does not offer a previous credential's declared namespaces to a job-scoped key", async () => {
  const declared = vi.spyOn(api, "getAuthNamespaces").mockResolvedValue({
    source: "", revision: "", loaded_at: "", last_reload: null,
    namespaces: [{ name: "default" }, { name: "finance-secret" }],
  } as never);
  vi.spyOn(api, "getJobs").mockResolvedValue([{ id: "a", alias: "a", namespace: "default" }, { id: "b", alias: "b", namespace: "marketing" }] as Job[]);
  const shared = create();
  const tree = (
    <SessionQueryProvider create={() => shared}>
      <NamespaceSwitcher />
    </SessionQueryProvider>
  );

  await login("csk_live_cluster", clusterViewer);
  const view = render(tree);
  await waitFor(() => expect(screen.getByTestId("namespace-switcher").textContent).toContain("finance-secret"));
  view.unmount();
  expect(declared).toHaveBeenCalledTimes(1);

  // Even if a client were shared, the hook consumes declared data only for the current principal.
  clearApiKey();
  await login("csk_live_scoped", scopedKey);
  render(tree);
  const select = await screen.findByTestId("namespace-switcher");
  expect(select.textContent).not.toContain("finance-secret");
  expect(select.textContent).toContain("marketing");
  expect(declared).toHaveBeenCalledTimes(1);
});
