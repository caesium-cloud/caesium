import { act, render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { IncidentsPage } from "@/features/incidents/IncidentsPage";
import { useNavCounts } from "@/features/jobs/useNavCounts";
import { resetNamespaceViewForTests, setStoredNamespace } from "../namespaceView";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children?: unknown }) => <a>{children as never}</a>,
  useNavigate: () => vi.fn(),
  useSearch: () => ({}),
}));

async function login(grants: Record<string, string>) {
  const whoami = { kind: "api_key", subject: "k", role: "", grants, namespaces: Object.keys(grants).sort(), is_scoped: false };
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => whoami }));
  expect(await apiKeyLogin("csk_live_shrink")).toBe("success");
}

function NavProbe() {
  useNavCounts();
  return null;
}

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  window.localStorage.clear();
  resetNamespaceViewForTests();
});
afterEach(() => vi.unstubAllGlobals());

it("clears an applied namespace filter once the switcher is hidden, so lists stop sending it", async () => {
  await login({ default: "viewer", finance: "viewer" });
  setStoredNamespace("finance");

  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({ agent_remediation_enabled: true } as never);
  const getIncidents = vi.spyOn(api, "getIncidents").mockResolvedValue({ incidents: [], total: 0 } as never);
  const getJobs = vi.spyOn(api, "getJobs").mockResolvedValue([]);
  const getTriggers = vi.spyOn(api, "getTriggers").mockResolvedValue([]);
  vi.spyOn(api, "getAtoms").mockResolvedValue([]);

  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <IncidentsPage />
      <NavProbe />
    </QueryClientProvider>,
  );

  // Two namespaces: the filter applies to the incident list, the approvals
  // strip and the sidebar counts.
  await waitFor(() => expect(getIncidents.mock.calls.some(([p]) => p?.namespace === "finance")).toBe(true));
  expect(getIncidents.mock.calls.some(([p]) => p?.needs_approval === true && p?.namespace === "finance")).toBe(true);
  expect(getJobs).toHaveBeenCalledWith("finance");
  expect(getTriggers).toHaveBeenCalledWith("finance");

  // The principal's grants shrink to a single namespace: the switcher hides.
  getIncidents.mockClear();
  getJobs.mockClear();
  getTriggers.mockClear();
  await act(async () => {
    await login({ default: "viewer" });
  });

  await waitFor(() => expect(window.localStorage.getItem("caesium.namespaceView")).toBeNull());
  await waitFor(() => {
    expect(getIncidents.mock.calls.some(([p]) => p?.namespace === undefined)).toBe(true);
    expect(getIncidents.mock.calls.some(([p]) => p?.needs_approval === true && p?.namespace === undefined)).toBe(true);
    // The unscoped jobs list is already cached, so it is not refetched; what
    // matters is that the finance-scoped one is no longer requested.
    expect(getJobs).not.toHaveBeenCalledWith("finance");
    expect(getTriggers).toHaveBeenCalledWith(undefined);
  });
  client.clear();
});
