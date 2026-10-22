import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type Job } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { NamespaceChip, NamespaceSwitcher } from "../NamespaceSwitcher";
import { resetNamespaceViewForTests } from "../namespaceView";

async function login(whoami: Record<string, unknown>) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => whoami }));
  expect(await apiKeyLogin("csk_live_switcher")).toBe("success");
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <NamespaceSwitcher />
    </QueryClientProvider>,
  );
}

const job = (namespace: string) => ({ id: namespace, alias: namespace, namespace }) as Job;

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  window.localStorage.clear();
  resetNamespaceViewForTests();
});
afterEach(() => vi.unstubAllGlobals());

describe("NamespaceSwitcher", () => {
  it("is hidden when the principal sees a single namespace", async () => {
    await login({ kind: "api_key", subject: "k", role: "", grants: { marketing: "runner" }, namespaces: ["marketing"], is_scoped: false });
    show();
    expect(screen.queryByTestId("namespace-switcher")).toBeNull();
  });

  it("lists a confined principal's namespaces and persists the choice", async () => {
    await login({ kind: "api_key", subject: "k", role: "", grants: { marketing: "runner", finance: "viewer" }, namespaces: ["finance", "marketing"], is_scoped: false });
    show();
    const select = await screen.findByTestId("namespace-switcher");
    expect(Array.from(select.querySelectorAll("option")).map((o) => o.textContent)).toEqual([
      "All namespaces",
      "finance",
      "marketing",
    ]);
    fireEvent.change(select, { target: { value: "marketing" } });
    expect((select as HTMLSelectElement).value).toBe("marketing");
    expect(window.localStorage.getItem("caesium.namespaceView")).toBe("marketing");
    fireEvent.change(select, { target: { value: "" } });
    expect(window.localStorage.getItem("caesium.namespaceView")).toBeNull();
  });

  it("drops a stored namespace the principal cannot see", async () => {
    window.localStorage.setItem("caesium.namespaceView", "secret-team");
    await login({ kind: "api_key", subject: "k", role: "", grants: { marketing: "runner", finance: "viewer" }, namespaces: ["finance", "marketing"], is_scoped: false });
    show();
    const select = (await screen.findByTestId("namespace-switcher")) as HTMLSelectElement;
    expect(select.value).toBe("");
  });

  it("offers a cluster viewer the declared namespaces plus those of its jobs", async () => {
    await login({ kind: "api_key", subject: "k", role: "viewer", cluster_role: "viewer", grants: { "*": "viewer" }, namespaces: ["*"], is_scoped: false });
    const declared = vi.spyOn(api, "getAuthNamespaces").mockResolvedValue({
      source: "", revision: "", loaded_at: "", last_reload: null,
      namespaces: [{ name: "default" }, { name: "finance" }],
    } as never);
    vi.spyOn(api, "getJobs").mockResolvedValue([job("default"), job("marketing")]);
    show();
    const select = await screen.findByTestId("namespace-switcher");
    await waitFor(() =>
      expect(Array.from(select.querySelectorAll("option")).map((o) => o.textContent)).toEqual([
        "All namespaces",
        "default",
        "finance",
        "marketing",
      ]),
    );
    expect(declared).toHaveBeenCalled();
  });

  it("never requests the declared namespaces for a job-scoped key", async () => {
    await login({ kind: "api_key", subject: "k", role: "viewer", cluster_role: "viewer", grants: { "*": "viewer" }, namespaces: ["*"], is_scoped: true });
    const declared = vi.spyOn(api, "getAuthNamespaces").mockResolvedValue({ namespaces: [] } as never);
    const jobs = vi.spyOn(api, "getJobs").mockResolvedValue([job("default"), job("marketing")]);
    show();
    await screen.findByTestId("namespace-switcher");
    expect(jobs).toHaveBeenCalled();
    expect(declared).not.toHaveBeenCalled();
  });
});

describe("NamespaceChip", () => {
  it("renders the namespace, defaulting a missing one", () => {
    const { rerender } = render(<NamespaceChip namespace="marketing" />);
    expect(screen.getByTestId("namespace-chip")).toHaveTextContent("marketing");
    rerender(<NamespaceChip />);
    expect(screen.getByTestId("namespace-chip")).toHaveTextContent("default");
  });
});
